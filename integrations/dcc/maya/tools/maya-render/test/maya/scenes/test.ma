//Maya ASCII 2024 scene
//Name: test.ma
//Codeset: UTF-8
requires maya "2024";
currentUnit -l centimeter -a degree -t film;
createNode transform -n "testCam";
	setAttr ".translate" -type "double3" 0 0 6 ;
createNode camera -n "testCamShape" -p "testCam";
	setAttr ".renderable" yes;
createNode transform -n "cube";
	setAttr ".rotate" -type "double3" 30 45 0 ;
createNode mesh -n "cubeShape" -p "cube";
createNode polyCube -n "polyCube1";
	setAttr ".width" 2;
	setAttr ".height" 2;
	setAttr ".depth" 2;
select -ne :defaultRenderGlobals;
	setAttr ".currentRenderer" -type "string" "mayaSoftware";
	setAttr ".animation" yes;
	setAttr ".startFrame" 1;
	setAttr ".endFrame" 2;
	setAttr ".byFrameStep" 1;
	setAttr ".outFormatControl" 0;
	setAttr ".putFrameBeforeExt" yes;
	setAttr ".periodInExt" 1;
	setAttr ".extensionPadding" 4;
	setAttr ".imageFormat" 32;
	setAttr ".imageFilePrefix" -type "string" "test";
select -ne :defaultResolution;
	setAttr ".width" 64;
	setAttr ".height" 64;
	setAttr ".deviceAspectRatio" 1;
connectAttr "polyCube1.output" "cubeShape.inMesh";
connectAttr "cubeShape.instObjGroups" ":initialShadingGroup.dagSetMembers" -nextAvailable;
// End of test.ma
